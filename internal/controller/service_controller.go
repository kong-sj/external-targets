package controller

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

/*
- //+kubebuilder:rbac 마커들은 make manifests명령어를 통해 controller-gen이 자동으로 인식하여 confg/rbac/role.yaml에 정의됨
- external-targets Operator의 Service Object 접근 권한 설정
- Service의 status 필드에 대한 읽기 권한과 Service 자체에 대한 생성, 업데이트, 수정 등에 대한 권한 설정
*/
//+kubebuilder:rbac:groups=core,resources=services/status,verbs=get
//+kubebuilder:rbac:groups=core,resources=services,verbs=create;update;patch;delete;get;list;watch

const (
	externalTargetsNamespace = "external-targets"       // ExternalName 서비스를 생성/관리할 네임스페이스 (미리 생성 필요)
	lbLinkerFinalizer        = "lb-linker/finalizer"    // Filnalizer 변수 값 추가
	managedAnnotationKey     = "linker.hsj.com/managed" // Annotaion Key 추가 변수
	managedAnnotationValue   = "true"                   // Annotaion Key에 대한 Value 변수 추가
)

// ServiceReconciler reconciles a Service object
type ServiceReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Log    logr.Logger
}

// sanitizeName은 쿠버네티스 오브젝트 이름 규칙 정제
func (r *ServiceReconciler) sanitizeName(name string) string {
	name = strings.ToLower(name)
	name = regexp.MustCompile(`[^a-z0-9-]+`).ReplaceAllString(name, "-")
	if len(name) > 63 { // Kubernetes리소스 이른은 63까지만 허용되므로 최대 길이 63자로 제힌
		name = name[:63]
	}
	name = strings.Trim(name, "-") // 이름의 앞뒤에 붙은 하이픈 제거
	if name == "" {
		// 매우 예외적인 경우, 기본 이름 또는 UUID 사용 고려
		return "default-ext-name-" + time.Now().Format("20060102150405")
	}
	return name
}

// Loadbalancer가 삭제될때 이와 연결된 ExternalName도 삭제하는 함수 추가
func (r *ServiceReconciler) cleanupExternalNameService(ctx context.Context, originalservice *corev1.Service, log logr.Logger) error {
	log.Info("Starting cleanup for associated ExternalName Service...")

	// 1. 이름규칙을 기반으로 삭제할 ExternalName 서비스의 이름을 결정
	externalNameServiceName := fmt.Sprintf("%s-%s-ext", originalservice.Name, originalservice.Namespace)
	externalNameServiceName = r.sanitizeName(externalNameServiceName)

	// 2. 삭제할 ExternalName을 정의
	svcToDelete := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      externalNameServiceName,
			Namespace: externalTargetsNamespace,
		},
	}

	// 3.  Delete API를 활용하여 ExternalName을 삭제
	log.Info("Attempting to delete ExternalName Service", "name", svcToDelete.Name)
	if err := r.Delete(ctx, svcToDelete); err != nil {
		// ExternalName이 이미 삭제되어 없다면 Notfound에러가 뜸
		if apierrors.IsNotFound(err) {
			log.Info("Associated ExternalName Service aleady deleted.")
			return nil
		}
		// Notfound에러가 아니라면 재시도를 위해 에러를 반환
		log.Error(err, "Failed to deleted ExternalName Service during cleanup.")
		return err
	}
	log.Info("Successfully deleted ExternalName Service.")
	return nil
}

// 서비스에 "managed" 어노테이션이 있는지 확인
func (r *ServiceReconciler) isManagedAnnotaion(service *corev1.Service) bool {
	// 어노테이션이 맵 자체가 없으면 false
	if service.Annotations == nil {
		return false
	}
	val, ok := service.Annotations[managedAnnotationKey]
	return ok && val == managedAnnotationValue
}

func (r *ServiceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("service", req.NamespacedName)

	// 1. 원본 Service 객체 가져오기
	var originalService corev1.Service
	if err := r.Get(ctx, req.NamespacedName, &originalService); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("Original Service not found. Ignoring since we are not handling deletions in this simplified version.")
			return ctrl.Result{}, nil
		}
		log.Error(err, "Failed to get Original Service")
		return ctrl.Result{}, err
	}

	// 2. Service가 LoadBalancer 타입인지 확인
	if originalService.Spec.Type != corev1.ServiceTypeLoadBalancer {
		log.Info("Service is not LoadBalancer type, ignoring.", "type", originalService.Spec.Type)
		return ctrl.Result{}, nil
	}

	// ExternalName Service 이름 결정(LB이름 + Namespace + ext) 예) my-sample-lb-3-default-ext
	externalNameServiceName := fmt.Sprintf("%s-%s-ext", originalService.Name, originalService.Namespace)
	externalNameServiceName = r.sanitizeName(externalNameServiceName)

	// 이 서비스가 Annotaion을 통한 관리 대상인지 확인
	isManaged := r.isManagedAnnotaion(&originalService)

	// Annotaion을 추가했다가 나중에 제거하게 되면 Operator의 관리대상에서 제외되나 Finalizer는 그대로 남게 되어 영원히 Terminating상태에 빠질 수 있음
	// 따라서 Annotation이 없는 LB의 경우 Finalizer가 있는지 확인하고, 있다면 ExternalName을 제거 후 Finalizer를 제거해줍니다.
	hasFinalizer := controllerutil.ContainsFinalizer(&originalService, lbLinkerFinalizer)

	if !isManaged {
		// 관리 대상이 아닌경우
		log.Info("Service is not managed by this operator")

		// 이전에 관리 대상이어서 Finalizer가 남아있는 경우
		if hasFinalizer {
			log.Info("Annotaion is gone, but finalizer exists. Running Cleanup to remove finalizer")

			// Finalizer만 제거
			controllerutil.RemoveFinalizer(&originalService, lbLinkerFinalizer)
			if err := r.Update(ctx, &originalService); err != nil {
				log.Error(err, "Failed to remove finalizer during hand-off")
				return ctrl.Result{}, err
			}
			log.Info("Finalizer removed.")
		}
		return ctrl.Result{}, nil
	}

	// 3. Service가 삭제중인지 확인 (DeletionTimestamp가 0이 아닌경우 삭제중인 것으로 판단)
	if !originalService.ObjectMeta.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&originalService, lbLinkerFinalizer) {
			log.Info("LoadBalancer Service is being deleted, starting cleanup logic.")

			// ExternalName 리소스 삭제 작업
			if err := r.cleanupExternalNameService(ctx, &originalService, log); err != nil {
				return ctrl.Result{}, err
			}

			// ExternalName 제거 작업이 성공하면 LoadBalancer에서 finalizer 제거
			log.Info("Cleanup finished. Removing finalizer")
			controllerutil.RemoveFinalizer(&originalService, lbLinkerFinalizer)
			if err := r.Update(ctx, &originalService); err != nil {
				log.Error(err, "Failed to remove finalizer")
				return ctrl.Result{}, err
			}
			log.Info("Cleanup finished, removing finalizer.")
		}
		return ctrl.Result{}, nil
	}

	// 3a. Service가 삭제중이 아닐 때: Finalizer가 없다면 추가
	if !controllerutil.ContainsFinalizer(&originalService, lbLinkerFinalizer) {
		log.Info("Adding Finalizer for the Loadbalacner Service.")
		controllerutil.AddFinalizer(&originalService, lbLinkerFinalizer)
		if err := r.Update(ctx, &originalService); err != nil {
			log.Error(err, "Failed to add finalizer")
			return ctrl.Result{}, err
		}
	}

	// 4. LoadBalancer의 외부 IP 또는 Hostname 정보 확인
	if len(originalService.Status.LoadBalancer.Ingress) == 0 {
		log.Info("LoadBalancer Ingress not yet available for Original Service.")
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil // 잠시 후 다시 시도
	}

	var externalTarget string
	ingress := originalService.Status.LoadBalancer.Ingress[0]
	if ingress.Hostname != "" {
		externalTarget = ingress.Hostname
	} else if ingress.IP != "" {
		externalTarget = ingress.IP
	} else {
		log.Info("LoadBalancer Ingress found, but no Hostname or IP available yet for Original Service.")
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}
	log.Info("Found external target for Original Service", "target", externalTarget)

	// 5. 기존 ExternalName Service 가져오기 시도
	foundExternalNameSvc := &corev1.Service{}
	externalNameSvcKey := types.NamespacedName{Name: externalNameServiceName, Namespace: externalTargetsNamespace}

	err := r.Get(ctx, externalNameSvcKey, foundExternalNameSvc)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// 5a. ExternalName Service가 없음: 새로 생성
			log.Info("ExternalName Service not found. Creating a new one.", "targetNamespace", externalTargetsNamespace, "targetName", externalNameServiceName)
			newExternalNameSvc := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      externalNameServiceName,
					Namespace: externalTargetsNamespace,
					Labels: map[string]string{
						"app.kubernetes.io/managed-by": "lb-linker-operator",
						"original-service-name":        originalService.Name,
						"original-service-namespace":   originalService.Namespace,
					},
				},
				Spec: corev1.ServiceSpec{
					Type:         corev1.ServiceTypeExternalName,
					ExternalName: externalTarget,
				},
			}

			if errCreate := r.Create(ctx, newExternalNameSvc); errCreate != nil {
				log.Error(errCreate, "Failed to create new ExternalName Service")
				return ctrl.Result{}, errCreate
			}
			log.Info("Successfully created new ExternalName Service", "externalName", externalTarget)
			return ctrl.Result{}, nil
		}
		// 기타 Get 오류
		log.Error(err, "Failed to get ExternalName Service")
		return ctrl.Result{}, err
	}

	// 5b. ExternalName Service가 이미 존재함: spec.externalName 필드 업데이트 확인
	if foundExternalNameSvc.Spec.ExternalName != externalTarget {
		log.Info("Existing ExternalName Service needs update.", "oldTarget", foundExternalNameSvc.Spec.ExternalName, "newTarget", externalTarget)

		// 변경 전 객체의 DeepCopy 생성
		beforeUpdateExternalNameSvc := foundExternalNameSvc.DeepCopy()

		// 실제 객체 필드 변경
		foundExternalNameSvc.Spec.ExternalName = externalTarget

		// r.Patch를 사용하여 변경된 부분만 업데이트 진행
		if errPatch := r.Patch(ctx, foundExternalNameSvc, client.MergeFrom(beforeUpdateExternalNameSvc)); errPatch != nil {
			log.Error(errPatch, "Failed to patch existing ExternalName Service")
			return ctrl.Result{}, errPatch
		}
		log.Info("Successfully patched ExternalName Service's externalName field.", "from", beforeUpdateExternalNameSvc.Spec.ExternalName, "to", foundExternalNameSvc.Spec.ExternalName)
	} else {
		log.Info("ExternalName Service is already up-to-date.")
	}

	return ctrl.Result{}, nil
}

func (r *ServiceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Service{}).
		Complete(r)
}
